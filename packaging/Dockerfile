FROM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cyapi ./cmd/cyapi
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* && useradd --uid 10001 --create-home cyapi
COPY --from=build /out/cyapi /usr/local/bin/cyapi
USER 10001:10001
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["cyapi"]
CMD ["serve", "--config", "/config/config.yaml"]
