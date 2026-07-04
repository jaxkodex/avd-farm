# Build both binaries as static executables.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/avdd ./cmd/avdd \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/avd  ./cmd/avd

# Runtime: alpine + the docker CLI (avdd shells out to it). No adb needed here.
FROM alpine:3.21
RUN apk add --no-cache docker-cli
COPY --from=build /out/avdd /out/avd /usr/local/bin/
ENTRYPOINT ["avdd"]
