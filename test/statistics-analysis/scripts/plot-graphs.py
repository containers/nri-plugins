#!/usr/bin/env python3

import matplotlib.pyplot as plt
import pandas as pd
import argparse
import sys
import os

LABEL_COLORS = {
    "baseline": "grey",
    "template": "red",
    "topology_aware": "green",
    "balloons": "blue"
}

def add_to_subplots_with_color(df, color):
    i = 1
    df_grouped = df.groupby("name")
    for title, group in df_grouped:
        ax = plt.subplot(4, 2, i)
        y_axis_label = group.columns[2]
        group.plot(x="timestamp", y=y_axis_label, ax=ax, legend=True, title=title, color=color)
        ax.set_xlabel("timestamp (seconds)")
        ax.set_ylabel(y_axis_label)
        i += 1

def add_to_subplots(df):
    i = 1
    df_grouped = df.groupby("name")
    for title, group in df_grouped:
        ax = plt.subplot(4, 2, i)
        y_axis_label = group.columns[2]
        group.plot(x="timestamp", y=y_axis_label, ax=ax, legend=True, title=title)
        ax.set_xlabel("timestamp (seconds)")
        ax.set_ylabel(y_axis_label)
        i += 1

def add_params(args):
    if args.increments == None and args.containers == None and args.workload == None and args.prefix == None:
        return

    ax = plt.subplot(4, 2, 8)
    ax.text(0.01, 0.91, "Test Run Parameters:")
    ax.get_xaxis().set_ticks([])
    ax.get_yaxis().set_ticks([])
    y = 0.7
    if args.prefix != None:
        ax.text(0.05, y, ("prefix: %s" % args.prefix))
        y = y - 0.1
    if args.increments != None:
        ax.text(0.05, y, ("number of increments: %s" % args.increments))
        y = y - 0.1
    if args.containers != None:
        ax.text(0.05, y, ("containers per increment: %s" % args.containers))
        y = y - 0.1
    if args.workload != None:
        ax.text(0.05, y, ("workload used: %s" % args.workload))

def createGraph(labels, inputFiles, args):
    plt.figure(figsize=(12, 12))

    for file in inputFiles:
        df = pd.read_csv(file)


        # Check if predetermined color exists.
        colorFound = False
        for key in LABEL_COLORS:
            if key in file:
                add_to_subplots_with_color(df, LABEL_COLORS[key])
                colorFound = True
                break
            
        if not colorFound:
            add_to_subplots(df)

    figure_axes = plt.gcf().axes
    handles, old_labels = figure_axes[0].get_legend_handles_labels()
    for ax in figure_axes:
        ax.get_legend().remove()

    plt.tight_layout()
    plt.figlegend(handles, labels, loc='lower right')

    add_params(args)

    plt.savefig(args.output)
    result = "created {}, input files used:".format(args.output)
    for file in inputFiles:
        result += "\n" + file

    return result

def scanCsvFiles(directory, labels, prefix):
    result = []
    directoryContents = os.listdir(directory)
    result_count = 0

    for label in labels:
        for element in directoryContents:
            if element.endswith(".csv"):
                if prefix != None and prefix != "":
                    if (prefix in element) and (label in element):
                        result.append(directory + "/" + element)
                        result_count += 1
                else:
                    if label in element:
                        result.append(directory + "/" + element)
                        result_count += 1

    if len(labels) != result_count:
        print("matching csv files for all labels not found")
        sys.exit(1)

    return result

def parseLabels(labelArg):
    labels = []
    stripped = []
    for l in labelArg.split(","):
        l = l.strip()
        labels.append(l)
        l = l.replace("_", "-")
        if l.endswith("-jaeger"):
            l = l[:-7]
        if l.endswith("-prometheus"):
            l = l[:-11]
        stripped.append(l)
    return labels, stripped

def main():
    parser = argparse.ArgumentParser(description="Get jaeger tracing data.")
    parser.add_argument("directory", help="directory containing output to scan")
    parser.add_argument("-l", "--labels", required=True, help="comma-separated list of labels used in the test setups")
    parser.add_argument("-o", "--output", required=True, help="the output file")
    parser.add_argument("-p", "--prefix", required=False, help="prefix of the output files")
    parser.add_argument("-i", "--increments", required=False, help="number of increments")
    parser.add_argument("-n", "--containers", required=False, help="containers per increment")
    parser.add_argument("-w", "--workload", required=False, help="workload")
    args = parser.parse_args(sys.argv[1:])

    labels, stripped = parseLabels(args.labels)
    inputFiles = scanCsvFiles(args.directory, labels, args.prefix)

    print(createGraph(stripped, inputFiles, args))

if __name__ == "__main__":
    main()
