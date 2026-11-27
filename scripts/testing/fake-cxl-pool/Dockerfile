# The image of fake-cxl-pool-controller, the pool controller of
# deploy/fake-cxl-pool-controller.yaml: the static binary of "make" alone,
# run as nobody. "make image" builds it from bin/ (.dockerignore keeps the
# rest of this directory out of the build context).
FROM scratch
COPY bin/fake-cxl-pool-controller /fake-cxl-pool-controller
USER 65534:65534
ENTRYPOINT ["/fake-cxl-pool-controller"]
