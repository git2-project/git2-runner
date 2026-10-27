FROM golang:1.26-bookworm

# psql to inspect the ci_svc schema on the shared dev DB, redis-cli to watch
# the ci:* keys and channels.
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    make \
    ca-certificates \
    postgresql-client \
    redis-tools \
    locales \
    && rm -rf /var/lib/apt/lists/*

# Debian's psql is a Perl wrapper that warns on every call when the locale is
# missing, so generate en_US.UTF-8.
RUN sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen \
    && locale-gen
ENV LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# Docker CLI only: the daemon is the `docker` compose service. Handy for
# looking at job containers by hand while working on the executor.
COPY --from=docker:29-cli /usr/local/bin/docker /usr/local/bin/docker

# Pinned: gopls v0.24+ needs Go 1.27. Bump with the base image.
RUN go install golang.org/x/tools/gopls@v0.23.0 \
    && go install golang.org/x/tools/cmd/goimports@v0.52.0

WORKDIR /workspace
