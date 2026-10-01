    location / {
        root /var/www/{{DIR}};
        # SPA fallback; for a multi-page site use: try_files $uri $uri/ =404;
        try_files $uri $uri/ /index.html;
    }
