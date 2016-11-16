#!/bin/sh -xue

docker build --build-arg CACHE_DATE=$(date +%Y-%m-%d:%H:%M:%S) -t red53_agent .
