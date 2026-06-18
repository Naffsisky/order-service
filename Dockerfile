FROM golang:1.22-alpine AS builder
WORKDIR /app

COPY . .
# go mod tidy: generate go.sum + download semua dependency
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -trimpath -o order-service .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/order-service .
EXPOSE 8080
CMD ["./order-service"]
