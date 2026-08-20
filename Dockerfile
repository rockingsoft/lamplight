FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/lamplight /usr/local/bin/lamplight

ENTRYPOINT ["/usr/local/bin/lamplight"]
