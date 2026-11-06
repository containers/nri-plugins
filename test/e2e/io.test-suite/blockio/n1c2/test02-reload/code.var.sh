blockio-setup
for mode in baseline plugin; do
    blockio-start "$mode" true
    for runtime in runc crun; do
        blockio-case before-reload "$runtime" gold
        blockio-case zero "$runtime" zero
        blockio-class-config 700
        blockio-case after-change "$runtime" gold

        # CRI-O warns and permits creation without applying the old snapshot.
        blockio-class-config malformed
        blockio-case bad-reload "$runtime" gold
        blockio-case unannotated-bad-reload "$runtime"
        blockio-class-config missing
        blockio-case missing-reload "$runtime" gold
        blockio-class-config 400
    done
    blockio-stop
done
blockio-compare
