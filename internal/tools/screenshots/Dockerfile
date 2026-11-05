# A demo fleet for the algod.top screenshots. run.sh builds the binaries
# into a temporary directory with these files and builds the image there.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends procps && rm -rf /var/lib/apt/lists/*
RUN useradd --system --home /var/lib/algorand --shell /bin/sh algorand && mkdir -p /var/lib/algorand /var/lib/algodtop /etc/algodtop \
 && chown algorand:algorand /var/lib/algorand /var/lib/algodtop /etc/algodtop
COPY algod /usr/bin/algod
COPY algodtop /usr/local/bin/algodtop
COPY --chown=algorand:algorand config.json /etc/algodtop/config.json
COPY start.sh /start.sh
COPY fakes.sh /fakes.sh
RUN chmod 600 /etc/algodtop/config.json && chmod 755 /start.sh /fakes.sh
USER algorand
ENV TERM=xterm-256color
CMD ["/start.sh"]
