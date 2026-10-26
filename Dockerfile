# Build on the host's native platform and cross-compile: Go crashes when run under QEMU emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.24 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/admission-tap .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/admission-tap /admission-tap
USER 65532:65532
ENTRYPOINT ["/admission-tap"]
