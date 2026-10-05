#!/bin/sh
# Build and test the OSS. No build tool, because javac and jar are enough for a
# service with no dependencies, and one fewer moving part is worth more here than
# anything Maven would add.
set -eu

cd "$(dirname "$0")"
OUT=build/classes
TEST_OUT=build/test-classes

rm -rf "$OUT" "$TEST_OUT" build/bswisp-oss.jar
mkdir -p "$OUT" "$TEST_OUT"

echo "compiling main..."
javac -Xlint:all -d "$OUT" $(find src/main -name '*.java')

echo "compiling tests..."
javac -Xlint:all -cp "$OUT" -d "$TEST_OUT" $(find src/test -name '*.java')

echo "running tests..."
java -cp "$OUT:$TEST_OUT" net.bswisp.oss.Tests

echo "packaging..."
jar --create --file build/bswisp-oss.jar --main-class net.bswisp.oss.Main -C "$OUT" .

echo "built build/bswisp-oss.jar"
