FROM python:3.11-slim

WORKDIR /app

# System deps
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential curl \
    && rm -rf /var/lib/apt/lists/*

# Python dependencies (layer cache)
COPY pyproject.toml ./
RUN pip install --no-cache-dir mcp openai chromadb pyyaml rich \
    rank-bm25 httpx pydantic streamlit kazoo aiomysql paramiko

# Application code
COPY diagflow/ ./diagflow/
COPY data/ ./data/
COPY demo/ ./demo/

VOLUME ["/data/chroma"]

EXPOSE 8501

ENV PYTHONUNBUFFERED=1
ENV CHROMA_PERSIST_DIR=/data/chroma

CMD ["streamlit", "run", "diagflow/conversation/web_app.py", \
     "--server.port=8501", "--server.address=0.0.0.0", \
     "--server.headless=true", "--browser.gatherUsageStats=false"]
