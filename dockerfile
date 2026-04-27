FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o vitals-api .

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/vitals-api .
EXPOSE 8080
CMD ["./vitals-api"]