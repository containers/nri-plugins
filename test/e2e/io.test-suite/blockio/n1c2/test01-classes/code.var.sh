# Compare startup-only configuration, annotation selection, all four throttle
# fields, overlapping device rules, and default/per-device BFQ weights.
blockio-setup
for mode in baseline plugin; do
    blockio-start "$mode" false
    for runtime in runc crun; do
        blockio-case unannotated "$runtime"
        blockio-case gold "$runtime" gold
        blockio-case multi-device "$runtime" bronze
        blockio-case container-override "$runtime" bronze gold
        blockio-case cri-override "$runtime" bronze empty gold
        blockio-case empty-override "$runtime" bronze ''
        blockio-case unknown "$runtime" unknown
        blockio-case zero "$runtime" zero

        # Without --blockio-reload, a changed file must not affect new containers.
        blockio-class-config 700
        blockio-case after-change "$runtime" gold
        blockio-class-config 400
    done
    blockio-stop
done
blockio-compare
