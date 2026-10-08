ARG GO_BUILD_IMAGE=golang:1.26.6-alpine
FROM ${GO_BUILD_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} GOMEMLIMIT=1GiB GOMAXPROCS=2
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=pinvou-account-platform-gomod-go126,target=/go/pkg/mod,sharing=locked go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,id=pinvou-account-platform-gomod-go126,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=pinvou-account-platform-gobuild-go126,target=/root/.cache/go-build,sharing=locked \
    case "$SERVICE" in account-service|identity-service|billing-service|token-gateway|token-portal|token-credential-portal|payment-simulator|event-relay|event-resources|approval-resources|migrate|account-security|workload-token-agent|token-policy-bootstrap|token-governance|token-release-prepare|token-command-executor|newapi-tls) ;; *) exit 1 ;; esac && \
    CGO_ENABLED=0 go build -mod=readonly -tags=nomsgpack -trimpath \
      -ldflags="-s -w -X github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver.version=${VERSION} -X github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver.commit=${COMMIT}" \
      -o /out/service "./cmd/${SERVICE}" && \
    CGO_ENABLED=0 go build -mod=readonly -tags=nomsgpack -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/service /service
COPY --from=build /out/healthcheck /healthcheck
COPY --from=build /src/migrations /migrations
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=6 CMD ["/healthcheck"]
ENTRYPOINT ["/service"]
