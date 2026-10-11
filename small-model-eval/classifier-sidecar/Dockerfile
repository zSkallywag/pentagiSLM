FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

ENV ZSC_MODEL=MoritzLaurer/deberta-v3-base-zeroshot-v1.1-all-33

# Bake the model into the image so startup is instant and offline afterwards
# (one download at build time instead of on every container start).
RUN python -c "from transformers import pipeline; pipeline('zero-shot-classification', model='${ZSC_MODEL}')"

COPY app.py .

EXPOSE 8077
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8077"]
