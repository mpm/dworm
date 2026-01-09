FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y \
    bash \
    curl \
    netcat-openbsd \
    procps \
    python3 \
    && rm -rf /var/lib/apt/lists/*

# Create a non-root user
RUN useradd -m -s /bin/bash developer

USER developer
WORKDIR /home/developer

# Simple test script that starts an HTTP server
COPY --chown=developer:developer test-server.sh /home/developer/test-server.sh
RUN chmod +x /home/developer/test-server.sh
