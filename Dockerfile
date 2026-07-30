# Build stage
FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /builds ./cmd/builds

# Runtime
FROM alpine:3.20
# git-lfs, and configured system-wide so git actually invokes it.
#
# Without it a repository that uses LFS still clones successfully — and lands
# every large file as a ~130-byte text pointer, which Docker then builds into a
# silently wrong image. Nothing errors. That is a far worse failure than not
# supporting LFS at all, which is why the tool and `git lfs install` belong
# together and neither is optional. The build agent hit the loud version of this
# on 2026-07-30; this is the quiet one.
RUN apk add --no-cache git git-lfs docker-cli docker-cli-compose ca-certificates curl \
 && git lfs install --system

COPY --from=builder /builds /usr/local/bin/builds

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/builds"]
