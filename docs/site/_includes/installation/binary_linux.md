```shell
curl -LO https://tuf.werf.io/targets/releases/{{ include.version }}/linux-{{ include.arch }}/bin/werf
sudo install ./werf /usr/local/bin/werf
```

