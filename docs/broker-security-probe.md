# Dedicated broker security probe

`tests/broker-probe` is a Go standard-library-only fixture, not the Build-System
application. Build from the repository root using
`tests/broker-probe/Dockerfile`. The final image is scratch and contains only the
probe executable and context marker fixtures. It has no shell, broker client
binary, application, daemon client, credentials, or application data.

## Runtime contract

The operator must run it with:

- UID/GID `10001:10001`, read-only root, all capabilities dropped,
  `no-new-privileges`, and `network=none`.
- Exactly one user-supplied bind mount: the broker **directory** at
  `/run/builds-broker`, read-only. Do not mount the Docker socket, a broker client
  binary, host configuration, SSH material, or any other host directory.
- No published ports. Health is served on `127.0.0.1:8080` inside the container.
- Appropriate CPU, memory and PID limits. Suggested: 0.25 CPU, 32 MiB memory,
  32 PIDs, `GOMAXPROCS=2`, and `GOMEMLIMIT=24MiB`.

Startup runs the checks once. The resulting JSON report is frozen and served at
`/health`. Any failed check makes this endpoint HTTP 503. `/probe healthcheck`
only reads this report. It does **not** repeat any security/broker request.
It prints bounded PASS/FAIL lines and returns nonzero unless the HTTP status and
every recorded check pass. HTTP startup alone is not success. The Dockerfile
uses a 3-second interval/timeout, 30-second start period and two retries.

The process has a fixed five-minute maximum lifetime starting before checks.
Only natural expiration of that lifetime with a passing report exits 0.
A failed report exits 1. SIGINT/SIGTERM or other parent-context cancellation
always exits 1, even if the startup report passed. A failure remains
servable and unhealthy until shutdown. The operator must check health/report,
not just whether the container is running, and must capture failure logs.

## Checks and limits

- Matching real/effective UID/GID; all five capability sets (`CapInh`, `CapPrm`,
  `CapEff`, `CapBnd`, `CapAmb`) zero; and `NoNewPrivs=1`. Supplementary groups
  must be absent or contain only 10001. Root and extra groups are forbidden.
- `/proc/mounts` must show the root and exact broker-directory mounts as `ro`.
  A write failure caused only by user permissions does not satisfy this check.
- No unexpected mounts beyond the broker directory and known Docker-managed
  system mounts/files. Standard `/proc`, `/sys`, `/dev` and Docker-created
  hosts/hostname/resolver file mounts are permitted. Known daemon, SSH,
  provider-key and host-configuration paths must be absent. Unrecognized mount
  layouts fail closed; this is a Docker/Linux fixture, not a universal sandbox
  scanner. It does not inspect or print file contents at secret paths.
- Only the loopback network interface is present.
- The broker directory is a directory, and `docker.sock` is a Unix socket.
- `/context/included.marker` has exact expected contents, while
  `/context/ignored.marker` must be absent.
- Raw broker ping returns `pong` with exit 0.
- Raw invalid requests receive exactly exit 1 with either `invalid request\n`
  or `request not allowed\n`, as appropriate. Busy, success, other errors,
  malformed/truncated replies and missing exit frames all fail.

Each raw request is one newline-terminated JSON header, followed by Unix
`CloseWrite`, then newline-delimited response frames through EOF. Build-negative
headers are followed by exactly 1024 zero bytes (a valid empty tar) before
`CloseWrite`. No checkout or real Dockerfile is sent. Other requests have no
payload. Each request is limited to two seconds and 64 KiB of response data.
Only fixed fixture inputs are used. Broker output is not echoed into the report.
The probe does not print environment variables, keys, configuration or process
argument lists, and makes no external-network/provider calls.

The permanently invalid cases cover unknown/run operations, a probe-only
unallowlisted build tag `registry.fandoster.com/broker-probe-denied:latest` with
`Dockerfile`, the same invalid tag combined with `../Dockerfile`, an unconfigured
probe service under the todo requested Compose path, a nonexistent probe-only
Compose path, duplicate ping keys, a null required operation, and an unknown
ping field. The combined invalid-tag/traversal request **does not independently
prove Dockerfile traversal validation**, because tag validation can reject it
first. No live push request, new external domain, production database service or
Food deployment is used. Food/db restrictions must instead be checked in broker
unit tests using an injected executor. No valid mutating commands are sent from
this fixture.

The expected exact denials reject `busy`, so running this fixture while the
outer controlled pipeline holds the broker's single-operation slot can verify
validation-before-busy behavior. The fixture deliberately does not acquire that
slot itself. A run against an idle broker alone does not prove validation
ordering under contention. The outer project pipeline separately verifies
allowed build, push and deploy behavior. Reading broker source is not proof that
the installed binary is identical.


### Socket authority and accepted trust boundary

A read-only bind mount of a Unix socket directory does **not** make its RPCs
read-only or restrict broker operations. For its bounded five-minute lifetime,
this fixture temporarily has the broker's **full existing allowlist authority**,
including any configured production build/push/deploy operations. UID 10001 is
an authorized broker peer. The fixture is accepted only as trusted repository
code with fixed safe inputs, a scratch image, `network=none`, and the bounded
lifetime. It has no arbitrary request/configuration/environment input for
selecting broker actions. Do not run an untrusted or modified image with this
mount. The broker allowlist is not changed or narrowed for this fixture.

## Context-ignore sentinel

`Dockerfile.dockerignore` excludes the negative marker and includes the positive
marker and source. The final-stage directory COPY intentionally copies every
fixture remaining in the build context. If Dockerfile-specific ignore handling
is bypassed, the negative marker reaches the image and the runtime fails.
There is currently no repository-root `.dockerignore`; if one is added, it must
not exclude `tests/broker-probe/fixtures/ignored.marker`. The marker contains no
secret. Never use ignore rules as a confidentiality boundary.

This tests **end-to-end context-ignore semantics**, including any client-side
filtering. It does not claim native Docker alone filters an stdin tar context.
The broker README's old native-ignore description is not authoritative for the
current client. This fixture does not prove kernel isolation or make arbitrary
untrusted Docker builds safe.

## Local tests

Use the installed Go 1.25 toolchain. No third-party dependencies are imported:

```text
GOCACHE=/root/build-agent-setup/broker-probe-fix/go-cache GOMAXPROCS=2 go test -p 2 ./tests/broker-probe
GOCACHE=/root/build-agent-setup/broker-probe-fix/go-cache GOMAXPROCS=2 go test -race -p 2 ./tests/broker-probe
```

Tests use only fake local Unix brokers, injected filesystem/network readers,
and local test HTTP servers. They cover exact wire input and half-close,
expected denials versus busy/success, response bounds and malformed/truncated
frames, report freezing, unhealthy startup, signal cancellation and bounded
lifetimes. The real five-minute container run and actual image/build/push/deploy
checks are separate operator integration checks.
