FROM debian:stable-slim

COPY linko /bin/linko

CMD ["/bin/linko"]