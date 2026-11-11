# Refreshing the README screenshots

Screenshots must show sample data only: the Liquibase `@dev-seed` fixtures and a fictional Claude
spend report. Never capture a stack with real data.

```bash
# 1. A throwaway stack on its own ports, branded as the fictional "Acme Robotics".
cat > /tmp/shots.env <<'EOF'
POSTGRES_PASSWORD=shots12345
WEB_PORT=8095
METRICS_PORT=9095
LIQUIBASE_CONTEXTS=dev-seed
BRAND_ORGANIZATION=Acme Robotics
BRAND_TEAM=Engineering
BRAND_SCOPE=Platform
BRAND_LINK_LABEL=Platform repo
BRAND_LINK_URL=https://dev.azure.com/acme/Platform/_git/Platform
EOF
docker compose --project-name ea-shots --env-file /tmp/shots.env up -d --build --wait web

# 2. A fictional spend report (the file name carries the date range the page displays).
node scripts/sample-spend-report.mjs /tmp/spend-report-sample-2026-08-01-to-2026-10-31.csv

# 3. Capture (refuses unless the API reports fixture/demo data).
node scripts/readme-screenshots.mjs http://127.0.0.1:8095 /tmp/spend-report-sample-2026-08-01-to-2026-10-31.csv docs/images

# 4. Look at every image, then remove the stack and its volume.
docker compose --project-name ea-shots down -v
```

The script shoots at 1440×900 CSS pixels with a device scale factor of 2, unpins the sticky filter
bar so it doesn't overlap element shots, and parks the mouse so no tooltip appears.
