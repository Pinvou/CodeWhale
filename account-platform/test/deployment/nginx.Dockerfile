# Test-only legacy parser/runtime compatibility. Never used as an application image.
FROM nginx:1.18-alpine
RUN sed -i 's|^http://dl-cdn\.alpinelinux\.org/|https://dl-cdn.alpinelinux.org/|' /etc/apk/repositories \
    && apk add --no-cache python3
ENTRYPOINT []
