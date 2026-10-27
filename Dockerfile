# BUILDER
FROM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
WORKDIR /src
COPY . .
RUN go mod tidy

RUN go build -trimpath -ldflags="-s -w" -o /out/gatekeeper .

# RUNNER
FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35
WORKDIR /
COPY --from=builder /out/gatekeeper /gatekeeper
EXPOSE 8080

ENV PORT=8080

ENTRYPOINT ["/gatekeeper"]