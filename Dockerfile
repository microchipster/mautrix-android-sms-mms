ARG GO_VERSION=1.27.3

FROM golang:${GO_VERSION}-alpine3.24 AS builder
RUN apk add --no-cache git ca-certificates build-base olm-dev zlib-dev
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/mautrix-android-sms-mms ./cmd/mautrix-android-sms-mms
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/android-sms-mms-adb-agent ./cmd/android-sms-mms-adb-agent

FROM alpine:3.24
RUN apk add --no-cache ca-certificates su-exec bash yq-go olm tzdata
COPY --from=builder /out/mautrix-android-sms-mms /usr/bin/mautrix-android-sms-mms
COPY --from=builder /out/android-sms-mms-adb-agent /usr/bin/android-sms-mms-adb-agent
COPY docker-run.sh /docker-run.sh
RUN chmod +x /docker-run.sh
VOLUME /data
EXPOSE 29321
ENTRYPOINT ["/docker-run.sh"]
