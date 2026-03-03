FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o cody .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates bash git curl
COPY --from=builder /build/cody /usr/local/bin/cody
RUN mkdir -p /root/.cody
VOLUME /root/.cody
ENTRYPOINT ["cody"]
CMD ["run"]
