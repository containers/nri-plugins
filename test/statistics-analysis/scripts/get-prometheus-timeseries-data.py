import requests
import argparse
import sys
import time

def createCsvFromResult(inputValues):
    result = "{},{},{}\n".format("name", "timestamp", "value")
    for key in inputValues:
        values = inputValues[key]["values"]
        for value in values:
            result += "{},{},{}\n".format(value["label"], str(value["time"]), value["value"])

    return result

def createTextOutputFromResult(inputValues):
    result = ""
    for key in inputValues:
        values = inputValues[key]["values"]
        metric = inputValues[key]["metric"]

        result += "\nquery: {}\n".format(key)
        result += "\nmetric:\n{:40s} {}\n".format("field", "value")
        for key in metric:
            result += "{:40s} {}\n".format(key, metric[key])

        result += "\n{} datapoints:\n{:70s} {:30s} {}\n".format(len(values), "query", "time", "value")
        for value in values:
            result += "{:70s} {:30s} {}\n".format(value["label"], str(value["time"]), value["value"])

    return result


def processValues(url, queries, labels, start, end):
    result = {}
    for i in range(len(queries)):
        query = queries[i]
        label = labels[i]
        queryOutput = getQueryOutput(url, query, start, end)
        if queryOutput["status"] != "success":
            print("request failed")
            sys.exit(1)
        
        resultList = queryOutput["data"]["result"]
        if len(resultList) == 0:
            print("no results from query")
            sys.exit(1)

        if len(resultList) > 1:
            print("error: more than one result found")
            sys.exit(1) 
        
        queryResult = resultList[0]
        values = []
        for value in queryResult["values"]:
            values.append({"label": label, "time": value[0] - start, "value": value[1]})

        values.sort(key=lambda datapoint: datapoint["time"])
        result[query] = {"values": values, "metric": queryResult["metric"]}

    return result

def getQueryOutput(url, query, start, end):
    r = requests.get(url + "/api/v1/query_range", { "query": query, "start": start, "end": end, "step": 15 })
    if (r.status_code != 200):
        print("error: {}, {}\n{}".format(r.status_code, r.reason, r.text))
        sys.exit(1)
    return r.json()

def handleQueryOutput(url, csv, queries, labels, start, end):
    result = processValues(url, queries, labels, start, end)
    
    if csv is not None:
        with open(csv, "w+") as csv_file:
            csv_file.write(createCsvFromResult(result))
            return "csv output written to " + csv

    return createTextOutputFromResult(result)

def parseCommaSeparatedString(arg):
    splitList = arg.split(",")
    for element in splitList:
        element.strip()
    return splitList

def main():
    parser = argparse.ArgumentParser(description="Get prometheus timeseries data. Example queries: "
                                                 "\"rate(container_cpu_usage_seconds_total[1m]\", "
                                                 "\"container_memory_usage_bytes\", and "
                                                 "\"container_memory_working_set_bytes\".")
    parser.add_argument("url", help="url for accessing prometheus")
    parser.add_argument("-q", "--queries", required=True, help="the Prometheus queries to use separated by commas (this unfortunately limits some Prometheus queries)")
    parser.add_argument("-l", "--labels", required=True, help="labels for csv data for each query")
    parser.add_argument("-d", "--duration", type=int, default=60, help="the duration in seconds which ends in the time the program was run")
    parser.add_argument("-s", "--start", type=int, help="the start of the Prometheus query interval as UTC timestamp in seconds")
    parser.add_argument("-e", "--end", type=int, help="the end of the Prometheus query interval as UTC timestamp in seconds")
    parser.add_argument("-c", "--csv", help="output csv file, otherwise print out json data")
    args = parser.parse_args(sys.argv[1:])


    queries = parseCommaSeparatedString(args.queries)
    labels = parseCommaSeparatedString(args.labels)
    if (len(labels) != len(queries)):
        print("there should be an equal amount of queries and labels")
        sys.exit(1)

    if args.start is not None and args.end is not None:
        print(handleQueryOutput(args.url, args.csv, queries, labels, args.start, args.end))
    else:
        currentTime = time.time()
        print(handleQueryOutput(args.url, args.csv, queries, labels, currentTime - args.duration, currentTime))

if __name__ == "__main__":
    main()
