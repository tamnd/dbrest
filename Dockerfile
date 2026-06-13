# Consumed by GoReleaser: it copies the already cross-compiled binary out of the
# build context rather than compiling, so the image build is fast and uses the
# same static binary every other artifact ships.
#
# GoReleaser builds one multi-platform image with buildx and stages each
# platform's binary under a $TARGETPLATFORM directory (e.g. linux/amd64/) in the
# build context, so the COPY line selects the right one through the automatic
# TARGETPLATFORM build arg.
FROM alpine:3.21

ARG TARGETPLATFORM

# ca-certificates for TLS to the database; tzdata for sane timestamps.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 dbrest

COPY $TARGETPLATFORM/dbrest /usr/bin/dbrest

USER dbrest

# 3000 is the API; 3001 is the admin server (/live, /ready, /metrics) when it is
# enabled. Configure the server with a mounted config file or the DBREST_*
# environment, for example:
#
#   docker run -p 3000:3000 \
#     -e DBREST_DB_BACKEND=postgres \
#     -e DBREST_DB_URI="postgres://web@db/app" \
#     ghcr.io/tamnd/dbrest
EXPOSE 3000 3001

ENTRYPOINT ["/usr/bin/dbrest"]
