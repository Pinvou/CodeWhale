FROM golang:1.26.5-alpine AS build

ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN test -n "$SERVICE" && \
    CGO_ENABLED=0 go build \
      -tags=nomsgpack \
      -trimpath \
      -ldflags="-s -w -X github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver.version=${VERSION} -X github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver.commit=${COMMIT}" \
      -o /out/service "./cmd/${SERVICE}" && \
	CGO_ENABLED=0 go build -tags=nomsgpack -trimpath -ldflags="-s -w" -o /out/token-policy-bootstrap ./cmd/token-policy-bootstrap && \
    CGO_ENABLED=0 go build -tags=nomsgpack -trimpath -ldflags="-s -w" -o /out/healthcheck ./cmd/healthcheck

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/service /service
COPY --from=build /out/healthcheck /healthcheck
COPY --from=build /out/token-policy-bootstrap /token-policy-bootstrap
COPY --from=build /src/migrations /migrations
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=6 CMD ["/healthcheck"]
ENTRYPOINT ["/service"]
