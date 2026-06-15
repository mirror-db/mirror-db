FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /relay .

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=build /relay /usr/local/bin/relay
ENTRYPOINT ["relay"]
CMD ["serve"]
