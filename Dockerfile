FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY . .
RUN go build -o jumia-server ./cmd/

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/jumia-server .
COPY --from=builder /app/web ./web
EXPOSE 8080
CMD ["./jumia-server"]
