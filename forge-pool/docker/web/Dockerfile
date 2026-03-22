FROM nginx:alpine

COPY web/index.html /usr/share/nginx/html/
COPY web/dist/ /usr/share/nginx/html/
COPY docker/web/nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
