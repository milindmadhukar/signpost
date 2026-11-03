FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /signpost . \
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
