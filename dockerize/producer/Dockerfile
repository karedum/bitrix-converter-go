FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod .

COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o producer ./cmd/producer

WORKDIR /app

FROM alpine:latest

COPY --from=builder /app/producer .

EXPOSE ${API_PORT}

CMD ["./producer"]