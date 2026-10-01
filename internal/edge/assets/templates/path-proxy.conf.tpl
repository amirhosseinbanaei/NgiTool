# Managed by edge — generated from edge.json. Change it with the edge CLI; edits here are overwritten.
# edge: {{TARGET}} -> {{DESC}}
location = {{PATH}} { return 301 {{PATH}}/$is_args$args; }

location {{PATH}}/ {
    set $upstream {{UPSTREAM}};
    {{STRIP}}
    proxy_pass http://$upstream;
    include /etc/nginx/edge/snippets/proxy.conf;
    proxy_set_header X-Forwarded-Prefix {{PATH}};
}
