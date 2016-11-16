#!/bin/bash -xue

./build.sh
docker run -t red53_agent
