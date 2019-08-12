#!/bin/bash
make clean-third-party clean protos clean-swagger-docs -j8

make install-toolchain -j8

make push-helm-ci

make terraform-test md-test golangci update-chart-deps install/yaml/

make third_party/ tls-certs build/chart/

make push-images -j8
