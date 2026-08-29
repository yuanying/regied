# netns 擬似 WAN テストベッドの実行環境。
#
# 開発機には nft / pppd / pppoe-server / dnsmasq が入っておらず、ホストにも
# 開発コンテナにも入れない方針なので、道具はこのイメージの中だけに置く。
# --privileged の兄弟コンテナとして起動し、その中で network namespace を組む
# （ADR 0010）。カーネルモジュールはホストで読み込むため、実行時に
# /lib/modules を読み取り専用で渡す。
FROM golang:1.27-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        conntrack \
        dnsmasq \
        iproute2 \
        iputils-ping \
        kmod \
        nftables \
        ppp \
        pppoe \
        procps \
        socat \
        tcpdump \
    && rm -rf /var/lib/apt/lists/*

# dnsmasq はパッケージのインストール時に常駐を始めてしまう。テストベッドが
# 必要になったときに netns の中で起こすので、既定では止めておく。
RUN if [ -x /etc/init.d/dnsmasq ]; then /etc/init.d/dnsmasq stop || true; fi \
    && rm -f /etc/systemd/system/multi-user.target.wants/dnsmasq.service

# 前提が欠けていたら黙って飛ばさずに落とす。
ENV REGIED_NETNS_REQUIRE=1

WORKDIR /workspace
