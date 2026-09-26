# goreleaser builds the binary and copies it in; see .goreleaser.yaml.
# For a local build: docker build --build-arg BIN=$(go build -o /dev/stdout ...) is
# not supported; use `goreleaser release --snapshot --clean` instead.
FROM gcr.io/distroless/static-debian12:nonroot
COPY safe_sql /usr/local/bin/safe_sql
COPY LICENSE THIRD_PARTY_LICENSES /usr/share/doc/safe_sql/
WORKDIR /work
ENTRYPOINT ["/usr/local/bin/safe_sql"]
CMD ["lint"]
