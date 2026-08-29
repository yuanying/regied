# The runtime environment for the netns pseudo-WAN testbed.
#
# The development machine has no nft / pppd / pppoe-server / dnsmasq, and the policy
# is to install them on neither the host nor the development container, so the tools
# live only inside this image. It is started as a sibling container with --privileged
# and the network namespaces are built inside it (ADR 0010). Kernel modules are loaded
# on the host, so /lib/modules is passed in read-only at run time.
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

# Installing the package starts a resident dnsmasq. The testbed starts its own inside
# a network namespace when it needs one, so shut the packaged instance off by default.
RUN if [ -x /etc/init.d/dnsmasq ]; then /etc/init.d/dnsmasq stop || true; fi \
    && rm -f /etc/systemd/system/multi-user.target.wants/dnsmasq.service

# If a prerequisite is missing, fail instead of silently skipping.
ENV REGIED_NETNS_REQUIRE=1

WORKDIR /workspace
