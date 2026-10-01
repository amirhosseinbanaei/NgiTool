    location / {
        set $upstream {{UPSTREAM}};
        proxy_pass http://$upstream;
        include /etc/nginx/edge/snippets/proxy.conf;
    }
