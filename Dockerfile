FROM --platform=$BUILDPLATFORM golang:1.25.6-alpine3.23 AS build

WORKDIR /usr/src

ADD go.mod go.sum ./
RUN go mod download && go mod verify

ADD . ./

ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /build/user ./cmd/user

FROM alpine:3.23

COPY --from=build /build/user /usr/local/bin/user

RUN apk upgrade --no-cache \
    && apk add tzdata

# API server
EXPOSE 1080

# Debug/profiling server
EXPOSE 1081

ENTRYPOINT ["user", "run"]
