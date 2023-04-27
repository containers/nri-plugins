import requests
import argparse
import sys

# OPERATION_NAMES = ["runtime.v1.RuntimeService/RunPodSandbox",
#                    "runtime.v1.RuntimeService/CreateContainer",
#                    "runtime.v1.RuntimeService/StartContainer",
#                    "runtime.v1.RuntimeService/StopContainer",
#                    "runtime.v1.RuntimeService/RemoveContainer",
#                    "runtime.v1.RuntimeService/StopPodSandbox",
#                    "runtime.v1.RuntimeService/RemovePodSandbox"]

def createCsvFromResult(processedDict):
    result = "{},{},{}\n".format("name", "timestamp", "duration (milliseconds)")
    for key in processedDict:
        operationSpans = processedDict[key]
        for span in operationSpans:
            # Note, times in microseconds (divide by 1000000 to get seconds)!
            result += "{},{},{}\n".format(key, str(span["startTime"] / 1000000), str(span["duration"] / 1000))

    return result

def createTextOutputFromResult(processedDict):
    result = ""
    for key in processedDict:
        operationSpans = processedDict[key]
        result += "{}, {} durations:\n{:40s} {}\n".format(key, len(operationSpans), "startTime", "duration")
        for span in operationSpans:
            # Note, times in microseconds (divide by 1000000 to get seconds)!
            result += "{:40s} {}\n".format(str(span["startTime"] / 1000000), str(span["duration"] / 1000))
        result += "\n"

    return result


def processSpansAndTraces(url, start, end):
    result = {
        "runtime.v1.RuntimeService/RunPodSandbox": [],
        "runtime.v1.RuntimeService/CreateContainer": [],
        "runtime.v1.RuntimeService/StartContainer": [],
        "runtime.v1.RuntimeService/StopContainer": [],
        "runtime.v1.RuntimeService/RemoveContainer": [],
        "runtime.v1.RuntimeService/StopPodSandbox": [],
        "runtime.v1.RuntimeService/RemovePodSandbox": []
    }
    for key in result:
        output = getQueryOutput(url, key, start, end)

        if output["errors"] != None:
            print("query for operation {} failed".format(key))

        traceList = output["data"]
        if len(traceList) == 0:
            print("no results for operation {}".format(key))

        for trace in traceList:
            spans = trace["spans"]
            for span in spans:
                operationName = span["operationName"]
                result[operationName].append(span)
        
        result[key].sort(key=lambda datapoint: datapoint["startTime"])

        for datapoint in result[key]:
            datapoint["startTime"] -= start

    return result

def getQueryOutput(url, operationName, start, end):
    return requests.get(url + "/api/traces", { "service": "containerd", "operation": operationName, "start": start, "end": end}).json()

def handleQueryOutput(url, csv, start, end):
    processedDict = processSpansAndTraces(url, start, end)

    if csv is not None:
        with open(csv, "w+") as csv_file:
            csv_file.write(createCsvFromResult(processedDict))
            return "csv output written to " + csv
    else:
        return createTextOutputFromResult(processedDict)
    

def main():
    parser = argparse.ArgumentParser(description="Get jaeger tracing data.")
    parser.add_argument("url", help="url for accessing jaeger")
    parser.add_argument("-c", "--csv", help="output csv file, otherwise print out json data")
    parser.add_argument("-s", "--start", type=int, help="the start of the Jaeger tracing query interval as UTC timestamp in seconds")
    parser.add_argument("-e", "--end", type=int, help="the end of the Jaeger tracing query interval as UTC timestamp in seconds")
    args = parser.parse_args(sys.argv[1:])

    # Jaeger tracing uses microseconds.
    print(handleQueryOutput(args.url, args.csv, int(args.start) * 1000000, int(args.end) * 1000000))

if __name__ == "__main__":
    main()
