# bandwidth

```bash
bandwidth --peer enode:abc123:3030
```

| Flag | Default | Description |
|------|---------|-------------|
| --peer | | Remote peer to connect to |
| --connections | 1 | Number of concurrent connections |
| --threshold | 3 | Minimum number of transactions to request |
| --min_bytes | 200000 | Minimum bytes a test must have before recording the bandwidth |
| --min_elapsed | 1ms | Minimum elapsed time a test must have before recording the bandwidth |
| --log_level | "info" | Log level (debug, info, warn, error) |

## Docker

```bash
docker build -t bandwidth .
docker run -it bandwidth --peer enode:abc123:3030
```
