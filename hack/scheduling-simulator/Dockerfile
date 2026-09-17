# HCP scheduling simulator — reproducible container.
FROM python:3.12-slim

WORKDIR /app

# Install deps first for layer caching.
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# App code + prebuilt data (profiles.json, real_pods_legacy.json, skus.yaml).
COPY . .

# Drop any local venv that may have been copied in.
RUN rm -rf venv

EXPOSE 8099

# Bind to 0.0.0.0 so the port is reachable from the host.
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8099"]
