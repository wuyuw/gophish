# Runtime container
FROM golang:1.18

USER root

RUN mkdir /opt/gophish

# RUN useradd -m -d /opt/gophish -s /bin/bash app

#RUN apt-get update && \
#	apt-get install --no-install-recommends -y jq libcap2-bin && \
#	apt-get clean && \
#	rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

WORKDIR /opt/gophish

COPY . /opt/gophish

# RUN chown app. config.template.json
RUN mv config.template.json config.json

RUN go build -v .

RUN #setcap 'cap_net_bind_service=+ep' /opt/gophish/gophish


RUN sed -i 's/127.0.0.1/0.0.0.0/g' config.json
RUN touch config.json.tmp

EXPOSE 3333 5000 8080 8443 80

CMD ["./docker/run.sh"]
