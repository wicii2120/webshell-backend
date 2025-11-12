FROM golang:1.23.6

ENV GOPROXY=https://goproxy.cn,direct \
    WEBSHELL_CONNECTION_TIMEOUT=10 \
    WEBSHELL_FS_ROOT=/root \
    WEBSHELL_PTY_CWD=/root

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

EXPOSE 1234
CMD ["go", "run", "main.go"]
