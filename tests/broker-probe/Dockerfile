# Build context: repository root. This is not the Build-System application.
FROM golang:1.25-alpine AS builder
ENV GOMAXPROCS=2
WORKDIR /src
COPY go.mod ./
COPY tests/broker-probe/main.go ./tests/broker-probe/main.go
RUN GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /probe ./tests/broker-probe

FROM scratch
COPY --from=builder /probe /probe
# Directory COPY is intentional: ignoring Dockerfile.dockerignore leaks the
# negative sentinel into /context and MUST make startup verification fail.
COPY tests/broker-probe/fixtures/ /context/
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=3s --timeout=3s --start-period=30s --retries=2 CMD ["/probe", "healthcheck"]
ENTRYPOINT ["/probe"]
