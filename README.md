## Contract Bindings

The contract bindings in `contracts/EthJobEscrow.go` are generated from the smart contract ABI. When the smart contract changes, you'll need to update these bindings.

### Updating Contract Bindings

1. Get the new ABI from the smart contract repository
2. Place the new ABI in `contracts/EthJobEscrow.abi`
3. Generate new bindings:
   ```bash
   abigen --abi=contracts/EthJobEscrow.abi --pkg=contracts --out=contracts/EthJobEscrow.go
   ```

## Installation

```bash
go get github.com/Fahedafzaal/go-integration
```

## Usage

See the [Integration Guide](INTEGRATION_GUIDE.md) for detailed usage instructions.

## Development

1. Clone the repository
2. Install dependencies:
   ```bash
   go mod tidy
   ```
3. Run tests:
   ```bash
   go test ./...
   ```

## License
