# The image of the pods of the pool-shared-fs and pool-fs demos: python3
# for the demo tools, mkfs.erofs for the writer of pool-shared-fs and
# mkfs.ext4, mkfs.xfs and mkfs.btrfs for the writer of pool-fs. demo setup
# pool-shared-fs and pool-fs build it (localhost/cxl-demo-tools:<sha256 of
# this file>) and import it into the nodes as localhost/cxl-demo-tools:demo.
FROM docker.io/library/python:3-alpine
RUN apk add --no-cache erofs-utils e2fsprogs xfsprogs btrfs-progs
