
FROM golang:1.25.6-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o alertory ./cmd/app

FROM alpine:3.18

RUN apk add --no-cache ca-certificates && \
    addgroup -g 65532 -S alertory && \
    adduser -u 65532 -S -G alertory -H alertory

WORKDIR /app

COPY --from=builder /app/alertory .

ENV PORT=8080

EXPOSE 8080

# Runs as a fixed non-root UID so it works under a `runAsNonRoot: true`
# pod security context out of the box (see charts/alertory's default
# podSecurityContext) without needing an arbitrary-UID workaround.
USER 65532:65532

CMD ["./alertory"]
