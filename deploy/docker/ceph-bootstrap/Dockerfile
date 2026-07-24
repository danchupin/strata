FROM quay.io/ceph/ceph:v21.1.0

COPY bootstrap.sh /usr/local/bin/bootstrap.sh
RUN chmod +x /usr/local/bin/bootstrap.sh

ENTRYPOINT ["/usr/local/bin/bootstrap.sh"]
