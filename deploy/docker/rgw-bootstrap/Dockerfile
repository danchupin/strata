FROM quay.io/ceph/ceph:v19.2.3

# curl is required for the compose healthcheck; the upstream ceph image ships
# it but pin the install so future image bumps don't silently regress.
RUN command -v curl >/dev/null 2>&1 || (dnf install -y curl && dnf clean all)

COPY rgw-entrypoint.sh /usr/local/bin/rgw-entrypoint.sh
RUN chmod +x /usr/local/bin/rgw-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/rgw-entrypoint.sh"]
