package controller

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"webshell/service/downloader"
	"webshell/websocket"
	"webshell/websocket/service/fs"
	"webshell/websocket/service/heartbeat"
	"webshell/websocket/service/shell"
	"webshell/websocket/service/upload"
)

type hostStruct struct {
	host string
	port int
}

var configuredHosts []hostStruct

func parseHostString(s string) (host string, port int, err error) {
	split := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(split) == 0 {
		err = errors.New("invalid host string")
		return "", 0, err
	}
	if len(split) == 1 {
		host = split[0]
		return host, 22, nil
	}
	host = split[0]
	port, err = strconv.Atoi(split[1])
	if err != nil {
		err = errors.New("invalid host string")
		return "", 0, err
	}
	return host, port, nil
}

func getConfiguredHosts() (hosts []hostStruct, resErr error) {
	if len(configuredHosts) > 0 {
		hosts = configuredHosts
		return
	}

	ev := os.Getenv("SSH_HOSTS")
	if ev == "" {
		err := errors.New("no configured ssh host")
		log.Println(err)
		resErr = err
		return
	}
	hostStrings := strings.Split(ev, ",")
	for _, hs := range hostStrings {
		h, p, err := parseHostString(hs)
		if err != nil {
			resErr = err
			log.Println(err)
			return
		}
		hosts = append(hosts, hostStruct{
			host: h,
			port: p,
		})
	}
	configuredHosts = hosts
	return
}

func StartLocalShell(c *gin.Context) {
	var req struct {
		Cwd string `json:"cwd"`
	}

	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	wsServer, err := websocket.NewServer(c.Writer, c.Request)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Register services
	shellService := shell.NewLocalService()
	fsService := fs.NewLocalService()
	heartbeatService := heartbeat.NewService()
	uploadService := upload.NewLocalService()

	wsServer.Register(shellService)
	wsServer.Register(fsService)
	wsServer.Register(uploadService)

	wsServer.RegisterPassive(heartbeatService)

	wsServer.Start()
}

type SSHController struct {
	Clients map[string]*ssh.Client
	*sync.RWMutex
	downloaders map[string]downloader.Downloader
}

func NewSSHController() *SSHController {
	return &SSHController{
		Clients: make(map[string]*ssh.Client),
		RWMutex: &sync.RWMutex{},
	}
}

type sshInfo struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

func dialSSH(addr string, config *ssh.ClientConfig) (client *ssh.Client, err error) {
	const maxAttempts = 3
	delay := 200 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		client, err = ssh.Dial("tcp", addr, config)
		if err == nil {
			return client, err
		}
		if attempt == maxAttempts {
			break
		}
		fmt.Printf("dial failed (attempt %d/%d): %v, retrying in %v\n",
			attempt, maxAttempts, err, delay)
		time.Sleep(delay)
		delay *= 2
	}
	return nil, fmt.Errorf("dial failed after %d attempts: %w", maxAttempts, err)
}

func getSSHClient(info *sshInfo, config *ssh.ClientConfig) (client *ssh.Client, err error) {
	if info.Host != "" {
		if info.Port == 0 {
			info.Port = 22
		}
		return dialSSH(fmt.Sprintf("%s:%d", info.Host, info.Port), config)
	}
	hosts, err := getConfiguredHosts()
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, errors.New("no configured hosts available")
	}

	const maxHostsToTry = 3
	randSrc := rand.New(rand.NewSource(time.Now().UnixNano()))
	perm := randSrc.Perm(len(hosts))
	tries := min(len(hosts), maxHostsToTry)

	for i := range tries {
		h := hosts[perm[i]]
		addr := fmt.Sprintf("%s:%d", h.host, h.port)
		client, err = dialSSH(addr, config)
		if err == nil {
			return client, nil
		}
		log.Printf("failed to dial host %s: %v", addr, err)
	}

	return nil, fmt.Errorf("failed to dial any configured host after %d attempts: %w", tries, err)
}

func (sc *SSHController) LoginSSH(c *gin.Context) {
	var sshInfo sshInfo
	if err := c.ShouldBindJSON(&sshInfo); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if sshInfo.Port == 0 {
		sshInfo.Port = 22
	}

	config := &ssh.ClientConfig{
		User:            sshInfo.Username,
		Auth:            []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // Note: In production, use proper host key verification
	}

	if sshInfo.Password != "" {
		config.Auth = append(config.Auth, ssh.Password(sshInfo.Password))
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "No authentication method provided"})
		return
	}

	client, err := getSSHClient(&sshInfo, config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	id := uuid.NewString()
	sc.Lock()
	sc.Clients[id] = client
	sc.Unlock()

	c.JSON(http.StatusOK, gin.H{"id": id})
}

func (sc *SSHController) StartSSHShell(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SSH client ID"})
		return
	}

	sc.RLock()
	sshClient, exists := sc.Clients[id]
	sc.RUnlock()

	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SSH client ID"})
		return
	}

	// Create websocket server
	wsServer, err := websocket.NewServer(c.Writer, c.Request)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Create SFTP service using the SSH connection
	fsService, err := fs.NewSFTPService(sshClient)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	fsSvc := fsService.(*fs.FSService)
	sftpClient := fsSvc.FS.(*fs.SFTPFileSystem).Client

	uploadService := upload.NewSFTPService(sftpClient)
	shellService := shell.NewSSHService(sshClient)
	heartbeatService := heartbeat.NewService()

	// 在创建 SFTP 服务的同时创建下载器
	sftpDl, err := downloader.NewSFTPDownloader(sshClient)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sc.Lock()
	if sc.downloaders == nil {
		sc.downloaders = make(map[string]downloader.Downloader)
	}
	sc.downloaders[id] = sftpDl
	sc.Unlock()

	// Register all services
	wsServer.Register(shellService)
	wsServer.Register(fsService)
	wsServer.Register(uploadService)

	wsServer.RegisterPassive(heartbeatService)

	wsServer.Start()
}

func (sc *SSHController) Download(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SSH client ID"})
		return
	}

	sc.RLock()
	dl, exists := sc.downloaders[id]
	sc.RUnlock()

	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SSH client ID"})
		return
	}

	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Path is required"})
		return
	}

	// Get file info first
	info, err := dl.Stat(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var reader io.ReadCloser
	if info.IsDir {
		reader, info, err = dl.DownloadDir(path)
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", info.Name))
	} else {
		reader, info, err = dl.Download(path)
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", info.Name))
		c.Header("Content-Length", fmt.Sprintf("%d", info.Size))
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer reader.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Status(http.StatusOK)

	// 使用大块缓冲区进行流式传输
	buffer := make([]byte, 32*1024)
	_, _ = io.CopyBuffer(c.Writer, reader, buffer)
}

func StartTCPShell(c *gin.Context) {
	var req struct {
		Host string `form:"host" binding:"required"`
		Port int    `form:"port"`
	}

	if err := c.ShouldBindQuery(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Port == 0 {
		req.Port = 23 // Default to telnet port
	}

	wsServer, err := websocket.NewServer(c.Writer, c.Request)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Register services
	shellService := shell.NewTCPService(req.Host, req.Port)
	heartbeatService := heartbeat.NewService()

	wsServer.Register(shellService)
	wsServer.RegisterPassive(heartbeatService)

	wsServer.Start()
}
