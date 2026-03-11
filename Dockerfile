FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/chatgpt-creator ./cmd/register

FROM alpine:3.21

WORKDIR /app

COPY --from=builder /out/chatgpt-creator /usr/local/bin/chatgpt-creator
COPY config.json ./config.json

ENV PORT=8080

EXPOSE 8080

CMD ["chatgpt-creator"]
