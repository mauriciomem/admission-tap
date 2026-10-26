FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/admission-tap .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/admission-tap /admission-tap
USER 65532:65532
ENTRYPOINT ["/admission-tap"]
