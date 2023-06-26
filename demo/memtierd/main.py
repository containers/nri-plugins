"""
This handles the requests from Grafana and returns the requested data in the correct format
"""

import json
import time
import subprocess

from fastapi import FastAPI


# File path variables to fetch data from
ZRAM_PATH =  "/sys/block/zram0/mm_stat"

MEMINFO_PATH = "/proc/meminfo"

# Adjust these based on your setup if needed
LOWPRIO_1_OUTPUT_PATH = "/tmp/memtierd/meme-pod-lowprio/memtierd.meme-pod-lowprio-1-container.output"
LOWPRIO_1_OUTPUT_FILE_NAME = "memtierd.meme-pod-lowprio-1-container.output"
HIGHPRIO_1_OUTPUT_PATH = "/tmp/memtierd/meme-pod-highprio/memtierd.meme-pod-highprio-1-container.output"
HIGHPRIO_1_OUTPUT_FILE_NAME = "memtierd.meme-pod-highprio-1-container.output"

# Point these to the files found at the data/ directory
LOWPRIO_1_TIME_SERIES_DATA_PATH = "./data/lowprio_1_time_series.json"
LOWPRIO_1_PAGE_FAULTS_DATA_PATH = "./data/page_faults_lowprio_1.json"
HIGHPRIO_1_TIME_SERIES_DATA_PATH = "./data/highprio_1_time_series.json"
HIGHPRIO_1_PAGE_FAULTS_DATA_PATH = "./data/page_faults_highprio_1.json"


app = FastAPI()


def write_json(data, filename):
	"""
	Write the json file changes
	"""

	with open(filename, "w") as file:
		json.dump(data, file, indent=4)


def add_page_fault_data(data, page_faults_data, page_faults_data_path, page_faults_data_key, file_name, page_fault_index):

	# Get the current total page faults
	curr_page_faults = page_faults_data[page_fault_index]["curr_page_faults"]
	curr_page_faults_minor = page_faults_data[page_fault_index]["page_faults_minor"]
	curr_page_faults_major = page_faults_data[page_fault_index]["page_faults_major"]

	with open(page_faults_data_path) as file:
		prev_data = json.load(file)

	page_faults_diff = 0
	page_faults_minor_diff = 0
	page_faults_major_diff = 0
	if len(prev_data[page_faults_data_key]) > 0:
		latest_entry = prev_data[page_faults_data_key][-1]
		prev_page_faults_total = latest_entry["curr_page_faults"]
		page_faults_diff = curr_page_faults - prev_page_faults_total
		page_faults_minor_diff = curr_page_faults_minor - prev_data[page_faults_data_key][-1]["page_faults_minor"]
		page_faults_major_diff = curr_page_faults_major - prev_data[page_faults_data_key][-1]["page_faults_major"]

	# Curr time in ms
	curr_time = round(time.time()*1000)

	prev_data[page_faults_data_key].append({
		"page_faults_diff": page_faults_diff,
		"page_faults_minor_diff": page_faults_minor_diff,
		"page_faults_major_diff": page_faults_major_diff,
		"curr_page_faults": curr_page_faults,
		"page_faults_minor": page_faults_data[page_fault_index]["page_faults_minor"],
		"page_faults_major": page_faults_data[page_fault_index]["page_faults_major"],
		"timestamp": curr_time
	})	

	with open(page_faults_data_path, "w") as file:
		json.dump(prev_data, file, indent=4)

	data[file_name]["page_faults_total"] = prev_data[page_faults_data_key]


def data_is_duplicate(data, new_entry, priority):
	if len(data[priority]) != 0:
		if any(d["VmSwap"] == new_entry["VmSwap"] for d in data[priority]) and any(d["VmRSS"] == new_entry["VmRSS"] for d in data[priority]):
			return True
	return False


def get_page_faults(pid):
	"""
	Get the page faults for a certain process
	"""

	process = subprocess.Popen(["ps", "-o", "min_flt,maj_flt", pid], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
	stdout, stderr = process.communicate()
	page_faults = stdout.decode("utf-8").split("\n")
	page_faults = page_faults[1].split(" ")
	page_faults = [x for x in page_faults if x.strip()]

	return page_faults


def handle_page_faults():
	"""
	Handle the page fault data
	"""

	# Get the process PID's
	process = subprocess.Popen(["pgrep", "memtierd"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
	stdout, stderr = process.communicate()
	memtierd_process_ids = stdout.decode("utf-8").split("\n")

	# Get the configuration for the PID
	memtierd_processes = []
	for pid in memtierd_process_ids:
		if len(pid) < 1:
			continue
		process = subprocess.Popen(["ps", "f", "-f", pid], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
		stdout, stderr = process.communicate()
		ps_output = stdout.decode("utf-8").split(" ")
		memtierd_config_path = ps_output[-1].strip("\n")

		# Check if process for certain config is already in the dictionary
		if not any(d["config_path"] == memtierd_config_path for d in memtierd_processes):
			memtierd_processes.append({"pid": pid, "config_path": memtierd_config_path})

	# Get the page faults
	for memtierd_process in memtierd_processes:
		page_faults = get_page_faults(memtierd_process["pid"])
		memtierd_process["curr_page_faults"] = int(page_faults[0]) + int(page_faults[1])
		memtierd_process["page_faults_minor"] = int(page_faults[0])
		memtierd_process["page_faults_major"] = int(page_faults[1])

	return memtierd_processes


def handle_zram_and_compressed_data(data):
	"""
	Handle the zram related stuff here
	"""

	zram_file_path = ZRAM_PATH
	proc_meminfo_path = MEMINFO_PATH

	orig_data_size = 0
	comp_data_size = 0
	mem_used_total = 0

	# Get the zram data
	with open(zram_file_path) as file:
		for line in file:
			zram_data = line.split()
			orig_data_size = zram_data[0]
			comp_data_size = zram_data[1]
			mem_used_total = zram_data[2]

	# Get the total memory of the system
	mem_total = 0
	with open(proc_meminfo_path) as file:
		for line in file:
			if "MemTotal" in line:
				mem_total = line.split(" ")[-2]
				break

	data["zram_and_compressed"] = {}

	# Turn mem_total to GB
	mem_total = int(mem_total) / 1000**2

	# Get total memory saved and mem saved percentage
	saved_memory_total = (int(orig_data_size) - int(mem_used_total)) / 1000000000
	saved_memory_percentage = (float(saved_memory_total) / float(mem_total)) * 100

	data["zram_and_compressed"]["save_memory_total"] = saved_memory_total
	data["zram_and_compressed"]["saved_memory_percentage"] = saved_memory_percentage

	compressed = 100 - (100 * int(comp_data_size) / int(orig_data_size))
	data["zram_and_compressed"]["compressed"] = compressed

	return data


def handle_json_data(filename, new_entry, prio):
	"""
	Modify the json data
	"""

	# Take the current time and add it to the datapoint
	curr_time = round(time.time()*1000) 
	new_entry["timestamp"] = curr_time

	with open(filename) as file:
		data = json.load(file)

		# Check if datapoint is already present in the dataset
		if data_is_duplicate(data, new_entry, prio):
			return

		time_series = data[prio]
		time_series.append(new_entry)

	# Modify the json file
	write_json(data, filename)


def handle_data(data, file_path, json_file_path = "", fetch_time_series_lowprio_1 = 0, fetch_time_series_highprio_1 = 0):
	"""
	Read the output file and format the data
	"""

	file_data = {}

	# VmSize - VmSwap - VmRSS
	other_size = 0

	with open(file_path) as file:
		for line in file:
			label, metric = line.strip().split(None, 1)
			label = label.replace(":", "")
			metric = metric.replace(" kB", "")
			if label == "VmSize":
				other_size = int(metric)
				continue
			other_size -= int(metric)
			file_data[label] = metric.strip()

	file_data["VmSize"] = other_size
	file_data["SwapAndRam"] = int(file_data["VmRSS"]) + int(file_data["VmSwap"])


	if "lowprio-1" in file_path and fetch_time_series_lowprio_1:
		handle_json_data(json_file_path, file_data, "time_series_lowprio_1")

	if "highprio-1" in file_path and fetch_time_series_highprio_1:
		handle_json_data(json_file_path, file_data, "time_series_highprio_1")

	# Take the filename from the path and add that field to "data"
	data[file_path.split("/")[-1]] = file_data

	return data


@app.get("/metrics")
def read_stats(fetch_time_series_highprio_1: int = 0, fetch_time_series_lowprio_1: int = 0, fetch_zram_data: int = 0,
	fetch_page_faults_highprio_1: int = 0, fetch_page_faults_lowprio_1: int = 0):
	"""
	Endpoint for getting the memtierd output data
	"""

	json_file_path = "time_series.json"

	data = {}

	highprio_1_file_path = HIGHPRIO_1_OUTPUT_PATH
	lowprio_1_file_path = LOWPRIO_1_OUTPUT_PATH

	highprio_1_file_name = HIGHPRIO_1_OUTPUT_FILE_NAME
	lowprio_1_file_name = LOWPRIO_1_OUTPUT_FILE_NAME

	file_paths = [highprio_1_file_path, lowprio_1_file_path]

	for file_path in file_paths:
		if fetch_time_series_lowprio_1:
			json_file_path = LOWPRIO_1_TIME_SERIES_DATA_PATH
			data = handle_data(data, file_path, json_file_path, fetch_time_series_lowprio_1=fetch_time_series_lowprio_1)
		if fetch_time_series_highprio_1:
			json_file_path = HIGHPRIO_1_TIME_SERIES_DATA_PATH
			data = handle_data(data, file_path, json_file_path, fetch_time_series_highprio_1=fetch_time_series_highprio_1)
		else:
			data = handle_data(data, file_path)


	# Get the zram data
	if fetch_zram_data:
		data = handle_zram_and_compressed_data(data)


	# Get the JSON file and append the new dictionary to it
	if fetch_time_series_lowprio_1 or fetch_time_series_highprio_1:
		with open(json_file_path) as file:
			json_data = json.load(file)
			if fetch_time_series_lowprio_1:
				data["time_series_lowprio_1"] = json_data["time_series_lowprio_1"]
			if fetch_time_series_highprio_1:
				data["time_series_highprio_1"] = json_data["time_series_highprio_1"]

	page_faults_data = handle_page_faults()

	for i in range(len(page_faults_data)):
		if fetch_page_faults_lowprio_1 and "lowprio-1" in page_faults_data[i]["config_path"]:
			curr_file_name = lowprio_1_file_name
			page_faults_data_path = LOWPRIO_1_PAGE_FAULTS_DATA_PATH
			page_faults_data_key = "page_faults_lowprio_1"
			add_page_fault_data(data, page_faults_data, page_faults_data_path, page_faults_data_key, curr_file_name,  i)

		if fetch_page_faults_highprio_1 and "highprio-1" in page_faults_data[i]["config_path"]:
			curr_file_name = highprio_1_file_name
			page_faults_data_path = HIGHPRIO_1_PAGE_FAULTS_DATA_PATH
			page_faults_data_key = "page_faults_highprio_1"
			add_page_fault_data(data, page_faults_data, page_faults_data_path, page_faults_data_key, curr_file_name,  i)

	return data
