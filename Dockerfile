FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o cody .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates bash git curl
COPY --from=builder /build/cody /usr/local/bin/cody
RUN addgroup -S cody && adduser -S -G cody -h /home/cody cody && mkdir -p /home/cody/.cody && chown -R cody:cody /home/cody
USER cody
VOLUME /home/cody/.cody
ENTRYPOINT ["cody"]
CMD ["run"]
