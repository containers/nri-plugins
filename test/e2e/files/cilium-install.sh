#!/usr/bin/env bash

timeout=30s
max_retries=5
count=0

ready=$(cilium status -o json --wait --wait-duration $timeout | jq .pod_state.cilium.Ready)
if [ $ready -eq 1 ]; then
    echo "Cilium already installed and ready."
    exit 0
fi

while [ $count -lt $max_retries ];
do
    cilium install
    if [ $? -ne 0 ]; then
	cilium uninstall
	sleep 5
	count=$(($count + 1))
	continue
    fi

    ready=$(cilium status -o json --wait --wait-duration $timeout | jq .pod_state.cilium.Ready)
    if [ $ready -eq 1 ]; then
	break
    fi

    echo "Cilium ready timeout, retrying"
    count=$(($count + 1))

    cilium uninstall
done

if [ $ready -ne 1 ]; then
    exit 1
fi

exit 0
