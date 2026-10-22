# BUILDER
FROM golang:1.26-alpine@sha256:27f2d3b83cfc5803c8078f8e730af0f7c431bf067afaec8f60a36fb763187cba AS builder
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