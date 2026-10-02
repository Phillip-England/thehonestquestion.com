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
USER 65532:65532
EXPOSE 8818
CMD ["/app/breathingroom"]
