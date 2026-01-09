#!/bin/bash
# Simple test server using Python's built-in HTTP server

PORT=${1:-8080}

echo "Starting test HTTP server on port $PORT..."
cd /tmp
echo "<html><body><h1>dworm test server</h1><p>Port: $PORT</p></body></html>" > index.html
python3 -m http.server $PORT
