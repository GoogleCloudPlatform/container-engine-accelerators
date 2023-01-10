FROM gcr.io/gke-release/debian-base:bullseye-v1.4.2-gke.7

RUN apt-get update && \
    apt-get install -y --no-install-recommends --allow-change-held-packages apt-transport-https ca-certificates curl gnupg && \
    echo "deb https://packages.cloud.google.com/apt google-fast-socket main" | tee /etc/apt/sources.list.d/google-fast-socket.list && \
    curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key add && \
    apt update &&  apt install -y --no-install-recommends google-fast-socket=0.0.5
