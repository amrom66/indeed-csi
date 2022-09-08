FROM golang:1.17-buster as builder
LABEL maintainer="linjinbao666@gmail.com"
WORKDIR /app
COPY . /app
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -ldflags ' -X main.version=0.0.1 -extldflags "-static"' -o indeed-csi .

FROM alpine
RUN apk add util-linux coreutils && apk update && apk upgrade
COPY --from=builder /app/indeed-csi /indeed-csi
ENTRYPOINT ["/indeed-csi"]