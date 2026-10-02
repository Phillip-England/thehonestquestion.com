FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /breathingroom .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libheif-examples ca-certificates && apt-get clean
WORKDIR /app
COPY --from=build /breathingroom /app/breathingroom
# Uppr bind mounts are owned by the workspace user (UID/GID 1000).
# Match that owner so private config files and SQLite remain accessible.
USER 1000:1000
EXPOSE 8818
CMD ["/app/breathingroom"]
