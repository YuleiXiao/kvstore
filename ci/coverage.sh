#!/bin/bash

MODE="mode: count"

# Grab the list of packages.
# Exclude the API and CLI from coverage as it will be covered by integration tests.
PACKAGES=`go list ./...`

# Create the empty coverage file.
echo $MODE > goverage.report

# Run coverage on every package.
for package in $PACKAGES; do
	output="$GOPATH/src/$package/coverage.out"
	go test -test.short -covermode=count -coverprofile=$output $package
	if [ -f "$output" ] ; then
		cat "$output" | grep -v "$MODE" >> goverage.report
	fi
        echo "resutl: $?"
done
