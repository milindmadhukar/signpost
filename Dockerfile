FROM golang:1.27-alpine AS build
# Tailwind's standalone CLI scans the templates and writes the one stylesheet,
# so the image needs no node and the page needs no CDN.
ARG TAILWIND_VERSION=v4.1.11
RUN apk add --no-cache libstdc++ libgcc \
 && wget -qO /usr/local/bin/tailwindcss \
      https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}/tailwindcss-linux-x64-musl \
 && chmod +x /usr/local/bin/tailwindcss
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN tailwindcss -i web/app.css -o web/static/app.css --minify \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /signpost . \
 && mkdir -p /data && chown 1000:1000 /data

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
