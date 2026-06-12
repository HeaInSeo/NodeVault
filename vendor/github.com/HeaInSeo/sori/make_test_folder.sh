#!/usr/bin/env bash
set -euo pipefail

# Root directory for the example volume
VOL_DIR="test-vol"

# Create directory structure
mkdir -p "$VOL_DIR/configs" "$VOL_DIR/docs"

# Create README.md
cat > "$VOL_DIR/README.md" << 'EOF'
This is a README
EOF

# Create configs/app.yaml
cat > "$VOL_DIR/configs/app.yaml" << 'EOF'
key: value
EOF

# Create docs/info.txt
cat > "$VOL_DIR/docs/info.txt" << 'EOF'
hello world
EOF

echo "Created example volume structure under '$VOL_DIR'"```

Save this as e.g. `create_example_vol.sh`, make it executable (`chmod +x create_example_vol.sh`), and run it to generate the desired structure.
