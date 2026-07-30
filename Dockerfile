# BUILDER
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src
COPY . .
RUN go mod tidy

RUN go build -trimpath -ldflags="-s -w" -o /out/gatekeeper .

# RUNNER
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1
WORKDIR /
COPY --from=builder /out/gatekeeper /gatekeeper
EXPOSE 8080

ENV PORT=8080

ENTRYPOINT ["/gatekeeper"]