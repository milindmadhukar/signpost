# Multi-platform build: the build stage always runs on the builder's own
# architecture ($BUILDPLATFORM) and Go cross-compiles for the target, so an
# arm64 or armv7 image builds at native speed with no emulation. The final
# stage is FROM scratch and runs nothing at build time.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
# Tailwind's standalone CLI scans the templates and writes the one stylesheet,
# and esbuild's standalone binary compiles the TypeScript, so the image needs
# no node and the page needs no CDN. Both run on the builder, so they are
# fetched for $BUILDARCH.
ARG TAILWIND_VERSION=v4.1.11
ARG ESBUILD_VERSION=0.28.2
ARG BUILDARCH
RUN case "$BUILDARCH" in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "unsupported build architecture: $BUILDARCH" >&2; exit 1 ;; \
    esac \
 && apk add --no-cache libstdc++ libgcc \
 && wget -qO /usr/local/bin/tailwindcss \
      https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-${arch}-musl \
 && chmod +x /usr/local/bin/tailwindcss \
 && wget -qO- https://registry.npmjs.org/@esbuild/linux-${arch}/-/linux-${arch}-${ESBUILD_VERSION}.tgz \
      | tar xz -C /opt \
 && mv /opt/package/bin/esbuild /usr/local/bin/esbuild && rm -rf /opt/package
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN tailwindcss -i web/app.css -o web/static/app.css --minify \
 && esbuild web/ts/app.ts --bundle --minify --target=es2022 --outfile=web/static/app.js \
 && esbuild web/ts/sw.ts --bundle --minify --target=es2022 --outfile=web/static/sw.js \
 && mkdir -p /data && chown 1000:1000 /data
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG RELEASE=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.release=${RELEASE}" -o /signpost .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /signpost /signpost
# Empty dir owned by 1000, so a fresh named volume mounted here inherits it.
COPY --from=build --chown=1000:1000 /data /data
USER 1000:1000
ENV SIGNPOST_DB=/data/signpost.db
EXPOSE 8080 8081
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/signpost", "healthcheck"]
ENTRYPOINT ["/signpost"]
CMD ["serve"]
