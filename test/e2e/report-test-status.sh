#!/bin/bash
#
# Report the status of the e2e tests. The script will find all the summary
# files under the current working directory and retrieve test case status.

for i in `find . -name 'summary*'`
do
    echo -n "`dirname $i | awk -F/ '{ print $2 " " $3 " " $4 " " $5 }'`: "
    tail -1 $i | awk '{ print $3 }'
done
