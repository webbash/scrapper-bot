FROM golang:1.26-bookworm AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bazaraki-html ./cmd/bazaraki-html

FROM debian:bookworm-slim
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/bazaraki-html /usr/local/bin/bazaraki-html
RUN install -d -o 10001 -g 10001 -m 0700 /var/lib/scrapper-bot \
    && install -o 10001 -g 10001 -m 0600 /dev/null /var/lib/scrapper-bot/.volume-init
WORKDIR /var/lib/scrapper-bot
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/bazaraki-html"]
CMD ["bot"]
