# Replicating the Memtierd demo

## Prerequisites
- Python 3
- Memtierd and Meme installed see [here](https://github.com/askervin/cri-resource-manager/tree/5FD_memtierd_devel/cmd/memtierd)
- NRI enabled on your container runtime (Containerd/CRI-O)
- Grafana dashboard ready to go
- Grafana [infinity](https://grafana.com/grafana/plugins/yesoreyeram-infinity-datasource/) data source plugin downloaded

## Installing the Memtierd grafana dashboard

- Go to the "Data Sources" tab and apply the Infinity data source (needed to handle the showcase the json data)
- Go to the "Dashboards" section on Grafana
- Click "New" and download the "memtierd-demo-grafana-dashboard.json" file and import it or use the dashboard id [18744](https://grafana.com/grafana/dashboards/18744-memtierd-demo/)
- Select Infinity data source as the data source

## Creating a swap in RAM

Needed in Ubuntu because zram.ko kernel module might not be installed in the system by default
```console
apt install linux-modules-extra-$(uname -r)
```

Create a 4GB compressed swap in RAM
```console
modprobe zram
echo 4G > /sys/block/zram0/disksize
mkswap /dev/zram0
swapon /dev/zram0
```

Check that the swap got created
```console
free -h
```

## Running the API

Edit the "path" variables found on the top of the main.py file to point to the correct data files in data/ aswell as zram and meminfo paths. When ran with the default workloads /tmp/memtierd directory will be created to read the output from, so unless the workload configurations are changed, those paths won't need editing.

Install FastAPI:
```console
pip install fastapi
```

Start the API with:
```console
uvicorn main:app --reload
```

Make sure the files in data/ are in the correct format:

Page fault files:
```json
{
    "page_faults_lowprio_1": [
    ]
}
```
```json
{
    "page_faults_highprio_1": [
    ]
}
```

Time series files:
```json
{
    "time_series_lowprio_1": [
    ]
}
```
```json
{
    "time_series_highprio_1": [
    ]
}
```

## Configuring the Memtierd NRI plugin:

Follow the steps found at cmd/memtierd and then:
```console
kubectl apply -f cmd/memtierd/templates/pod-memtierd.yaml
```

## Replicating the workloads

Install meme with the instructions found [here](https://github.com/intel/memtierd/blob/main/cmd/memtierd/README.md#install-memtierd-on-the-vm).

Then create a meme image, you can use this Dockerfile:
```dockerfile
FROM debian:stable-slim

COPY . .

RUN mv meme /bin

CMD ["meme", "-bs", "1G", "-bwc", "1", "-bws", "100M", "-bwi", "1s", "-bwod", "17M", "-brc", "0", "-p", "10s", "-ttl", "24h"]
```

Then push that to your image registry and point the high-prio and low-prio deployment files to pull from there.

Deploy the high priority workloads:
```console
kubectl apply -f templates/high-prio.yaml
```

Deploy the low priority workloads:
```console
kubectl apply -f templates/low-prio.yaml
```
