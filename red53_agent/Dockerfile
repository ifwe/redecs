FROM debian:jessie
MAINTAINER Misha Nasledov <misha@nasledov.com>

RUN apt-get update
RUN apt-get -y install curl

ARG CACHE_DATE=not_a_date

RUN curl -sL http://github.com/ifwe/redecs/releases/download/0.7/red53_agent -o /usr/local/bin/red53_agent
RUN chmod 0755 /usr/local/bin/red53_agent

CMD ["/usr/local/bin/red53_agent", "servicediscovery.local", "MY_REDIS_HOST"]
