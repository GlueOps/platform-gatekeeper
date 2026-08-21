# BUILDER
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /src
COPY . .
RUN go mod tidy

RUN go build -trimpath -ldflags="-s -w" -o /out/gatekeeper .

# RUNNER
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /
COPY --from=builder /out/gatekeeper /gatekeeper
EXPOSE 8080

ENV PORT=8080

ENTRYPOINT ["/gatekeeper"]