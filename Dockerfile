FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY . .
RUN go build -o agora-server ./cmd/

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/agora-server .
COPY --from=builder /app/web ./web
EXPOSE 8080
CMD ["./agora-server"]
